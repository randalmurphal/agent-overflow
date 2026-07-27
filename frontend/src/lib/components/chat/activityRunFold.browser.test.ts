// The automatic fold, against a real layout engine and a real Web Animations
// timeline.
//
// happy-dom can see that the clip eventually leaves the DOM, and the unit
// suite asserts exactly that — but there it leaves in one frame, because a
// zero-height box takes the fold's no-motion path. Everything that makes the
// fold a fold instead of a jump is geometry: that the box shrinks over several
// frames, that the newest row holds still against the closing edge while the
// older ones leave through the top, and that the run lands on its header with
// nothing left over.
import { describe, expect, it } from 'vitest';
// Real production cascade: the clip's cap and every row height below come from
// app.css, and the fold's start height is measured against them.
import '../../../app.css';
import { makeItem } from '../../../test/helpers/chat';
import { makeSettings } from '../../../test/helpers/settings';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { updateSetting } from '../../stores/settings.svelte';
import {
  mountTimeline,
  setupTimelineHarness,
  type QuietBottomOptions,
} from '../../../test/helpers/timelineBrowserHarness';
import type { Item } from '../../types/models';

setupTimelineHarness();

const QUIET_BOTTOM: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };
/** Enough rows to overflow the cap, so the fold has real height to close. */
const RUN_ROWS = 40;
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

function prose(id: string, index: number, threadId: string): Item {
  return makeItem({
    id,
    threadId,
    itemIndex: index,
    summary: `Reply ${id}: prose breaks the rail, so the run before it can never grow again.`,
    createdAt: index,
    updatedAt: index,
  });
}

function foldBox(scrollEl: HTMLElement): HTMLElement | null {
  return scrollEl.querySelector('[data-testid="activity-run-fold"]');
}

function requireFoldBox(scrollEl: HTMLElement): HTMLElement {
  const box = foldBox(scrollEl);
  if (!box) throw new Error('the run has no clip to fold');
  return box;
}

function newestRow(scrollEl: HTMLElement): HTMLElement {
  const found = scrollEl.querySelector(`[data-item-id="a${RUN_ROWS - 1}"]`);
  if (!(found instanceof HTMLElement)) throw new Error('the run\'s newest row is not mounted');
  return found;
}

describe('activity run — automatic fold', () => {
  const THREAD_ID = 'thread-run-fold';

  // Two runs: a finished one, which is the reference for what a header-only
  // run measures, and the live one under test. Prose between them is what closes
  // the first — activity on either side of prose is a separate run.
  const REFERENCE_ROWS = 3;

  function items(): Item[] {
    const built: Item[] = [];
    for (let i = 0; i < REFERENCE_ROWS; i += 1) built.push(tool(`b${i}`, i, THREAD_ID));
    built.push(prose('p0', REFERENCE_ROWS, THREAD_ID));
    for (let i = 0; i < RUN_ROWS; i += 1) {
      built.push(tool(`a${i}`, REFERENCE_ROWS + 1 + i, THREAD_ID));
    }
    return built;
  }

  function runs(scrollEl: HTMLElement): HTMLElement[] {
    return [...scrollEl.querySelectorAll<HTMLElement>('[data-testid="activity-run"]')];
  }

  async function mountCollapsedLiveRun() {
    // The harness installs GetSettings only; changing one needs the write side
    // too, and `updateSetting` swallows nothing — a missing mock surfaces as a
    // console error and the default silently stands, which is a header-only
    // run and four vacuous tests.
    setBindingMock('UpdateSettings', async (patch: unknown) =>
      makeSettings(patch as Parameters<typeof makeSettings>[0]));
    await updateSetting('activityRunDefault', 'collapsed');
    const mountedTimeline = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);
    const { scrollEl } = mountedTimeline;
    // The state the whole feature exists for: collapsed, and still showing its
    // work. If this ever renders header-only, every assertion below is vacuous.
    expect(scrollEl.querySelector('[data-testid="activity-run-header"]')).not.toBeNull();
    // Exactly one clip: the finished run is header-only, the live one is not.
    expect(scrollEl.querySelectorAll('[data-testid="activity-run-fold"]')).toHaveLength(1);
    const box = requireFoldBox(scrollEl);
    expect(box.getBoundingClientRect().height).toBeGreaterThan(0);
    return mountedTimeline;
  }

  /**
   * Move the reader off the clip's newest row WITHOUT landing in the paging
   * runway at its top, which would mount an earlier chunk and compensate the
   * position — a second thing moving `scrollTop`, and nothing to do with the
   * fold. The wheel is not decoration: leaving the newest row is a decision,
   * and the run reads it from geometry only when a gesture produced it.
   */
  async function readerStepsInside(clip: HTMLElement): Promise<number> {
    const target = Math.round((clip.scrollHeight - clip.clientHeight) / 2);
    clip.dispatchEvent(new WheelEvent('wheel', { deltaY: -120, bubbles: true }));
    clip.scrollTop = target;
    await raf();
    await raf();
    return target;
  }

  /**
   * Prose closes the run, which is the only thing that starts a fold.
   *
   * Past the last member, deliberately: an index inside the run splits it into
   * two runs and leaves the tail one live, which is correct behaviour and
   * completely useless as a fixture.
   */
  function finishRun(pane: { upsertItem(item: Item): void }): void {
    pane.upsertItem(prose('p1', REFERENCE_ROWS + RUN_ROWS + 1, THREAD_ID));
  }

  it('closes over several frames instead of vanishing between two', async () => {
    const { pane, scrollEl } = await mountCollapsedLiveRun();
    const startPx = requireFoldBox(scrollEl).getBoundingClientRect().height;

    finishRun(pane);

    // Sampled every frame from the moment the run finishes. A jump would show
    // up as a single sample: full height, then nothing.
    //
    // The bound only has to outlast the longest fold `activityRunFold.ts` can
    // produce (`FOLD_MAX_MS`) — the loop exits the frame the box reaches zero,
    // so spare frames cost nothing, and a generous bound keeps this test from
    // failing the next time the duration is tuned up.
    const heights: number[] = [];
    for (let i = 0; i < 90 && (i === 0 || heights[heights.length - 1] > 0); i += 1) {
      await raf();
      const box = foldBox(scrollEl);
      heights.push(box ? box.getBoundingClientRect().height : 0);
    }

    const partial = heights.filter((h) => h > DRIFT_PX && h < startPx - DRIFT_PX);
    expect(partial.length).toBeGreaterThanOrEqual(3);
    // Monotone: the fold only ever closes. A height that grew mid-fold would
    // mean the clip is still reflowing inside the box it is being folded in.
    for (let i = 1; i < heights.length; i += 1) {
      expect(heights[i]).toBeLessThanOrEqual(heights[i - 1] + DRIFT_PX);
    }
    expect(heights[heights.length - 1]).toBe(0);
  });

  it('closes onto the run\'s newest row, not off the top of it', async () => {
    const { pane, scrollEl } = await mountCollapsedLiveRun();
    // Read before the fold starts: the box below IS the animating element, so
    // its height has to be captured, not re-read.
    const startPx = requireFoldBox(scrollEl).getBoundingClientRect().height;
    // What the reader is looking at when the fold starts: the last thing the
    // run did, sitting at the bottom of the clip.
    const gapBefore = requireFoldBox(scrollEl).getBoundingClientRect().bottom
      - newestRow(scrollEl).getBoundingClientRect().bottom;

    finishRun(pane);

    // Mid-fold, deliberately: the claim is about what the animation LOOKS
    // like, and only a partially closed box can answer it.
    let gapDuring: number | null = null;
    for (let i = 0; i < 40; i += 1) {
      await raf();
      const live = foldBox(scrollEl);
      if (!live) break;
      const height = live.getBoundingClientRect().height;
      if (height <= DRIFT_PX || height >= startPx - DRIFT_PX) continue;
      gapDuring = live.getBoundingClientRect().bottom
        - newestRow(scrollEl).getBoundingClientRect().bottom;
      break;
    }

    expect(gapDuring).not.toBeNull();
    // The clip is pinned to the closing edge, so the newest row keeps its
    // place against it and the rows above leave through the top. Anchored the
    // other way the newest row would be the first thing cut off — the run
    // would go blank while it closed.
    expect(Math.abs((gapDuring ?? 0) - gapBefore)).toBeLessThanOrEqual(DRIFT_PX);
  });

  it('lands where a run that was never open already is', async () => {
    // The end state has to be indistinguishable from an ordinary collapsed
    // run, or the last frame of the fold is itself a jump. Measured against the
    // finished run in the same fixture rather than against the header element,
    // because the run's own box adds a line of its own either way — the claim is
    // that the fold leaves NOTHING extra behind, and only another header-only
    // run can price that.
    const { pane, scrollEl } = await mountCollapsedLiveRun();
    const runRows = runs(scrollEl);
    expect(runRows).toHaveLength(2);
    const [reference, subject] = runRows;
    expect(reference.dataset.live).toBe('false');
    expect(subject.dataset.live).toBe('true');
    const referencePx = reference.getBoundingClientRect().height;

    finishRun(pane);
    await waitFor(() => foldBox(scrollEl) === null, 'the fold to finish');

    expect(subject.querySelector('[data-testid="activity-run-header"]')).not.toBeNull();
    expect(subject.dataset.collapsed).toBe('true');
    expect(subject.dataset.live).toBe('false');
    // No leftover box, and no residual height from the animation's fill.
    expect(Math.abs(subject.getBoundingClientRect().height - referencePx))
      .toBeLessThanOrEqual(DRIFT_PX);
  });

  it('does not fold a run the reader is standing inside', async () => {
    const { pane, scrollEl } = await mountCollapsedLiveRun();
    const clip = scrollEl.querySelector('[data-testid="activity-run-clip"]') as HTMLElement;
    // Vacuity guard: a clip that cannot scroll has no inside to stand in.
    expect(clip.scrollHeight).toBeGreaterThan(clip.clientHeight);

    const parked = await readerStepsInside(clip);

    finishRun(pane);
    for (let i = 0; i < 40; i += 1) await raf();

    // Still open, and still where they left it.
    expect(foldBox(scrollEl)).not.toBeNull();
    expect(Math.abs(clip.scrollTop - parked)).toBeLessThanOrEqual(DRIFT_PX);

    // Returning to the newest row pays the debt.
    clip.dispatchEvent(new WheelEvent('wheel', { deltaY: 120, bubbles: true }));
    clip.scrollTop = clip.scrollHeight;
    await waitFor(() => foldBox(scrollEl) === null, 'the deferred fold to run');
  });
});
