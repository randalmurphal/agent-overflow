// The two places an activity run writes its own `scrollTop`, against a real
// layout engine: prepend compensation when the reader mounts an earlier chunk,
// and the jump that centers an item the reader searched for.
//
// Both are pure geometry, so happy-dom cannot see either — every rect there is
// zero, which makes "the reading position held" and "the target is in view"
// vacuously true. The unit suite (ActivityRun.test.ts) asserts which rows are
// in the DOM; this file asserts where they ended up on screen.
//
// Runs the REAL MessageTimeline over a real pane (real windowing, real
// ResizeObserver timing, real fonts) through the shared harness, because both
// behaviors are reached the way a user reaches them: a click on the boundary,
// and `pane.requestScrollToItem` from search / review / the jump tray.
import { describe, expect, it } from 'vitest';
// Real production cascade: the clip's cap, the rail indent, and row heights
// all come from app.css, and every number below is measured against them.
import '../../../app.css';
import { tick } from 'svelte';
import { makeItem } from '../../../test/helpers/chat';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import {
  mountTimeline,
  setupTimelineHarness,
  type QuietBottomOptions,
} from '../../../test/helpers/timelineBrowserHarness';
import { ACTIVITY_RUN_WINDOW_ROWS_DEFAULT as WINDOW_ROWS } from '../../utils/activityRunWindow';
import type { Item } from '../../types/models';

setupTimelineHarness();

// The mount settle only has to reach a quiet bottom; 2px absorbs
// fractional-DPR rounding, matching the other outcome suites.
const QUIET_BOTTOM: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };

// One row past the window plus a chunk would page twice; this pages once and
// leaves the boundary retiring, which is also the unit suite's shape.
const RUN_ROWS = WINDOW_ROWS + 10;
// Rects are fractional (fonts, borders, DPR). A failure here is the whole
// prepend delta — hundreds of px — so a pixel of tolerance costs nothing.
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
    summary: `Reply ${id}: prose breaks the rail, so the activity on either side of it is a separate run.`,
    createdAt: index,
    updatedAt: index,
  });
}

function runRow(clip: Element, itemId: string): HTMLElement {
  const found = clip.querySelector(`[data-item-id="${itemId}"]`);
  if (!(found instanceof HTMLElement)) throw new Error(`row ${itemId} is not mounted`);
  return found;
}

/** Row top relative to the clip's viewport — what the reader actually sees. */
function offsetInClip(clip: HTMLElement, itemId: string): number {
  return runRow(clip, itemId).getBoundingClientRect().top - clip.getBoundingClientRect().top;
}

function isFullyVisible(clip: HTMLElement, itemId: string): boolean {
  const row = runRow(clip, itemId).getBoundingClientRect();
  const view = clip.getBoundingClientRect();
  return row.top >= view.top - DRIFT_PX && row.bottom <= view.bottom + DRIFT_PX;
}

describe('activity run — prepend compensation', () => {
  // A HISTORICAL run, deliberately: only the last run gets a scroll
  // controller, so this isolates the compensation arithmetic from the spring.
  // The live run's own jump path is covered below, where the write states its
  // escape explicitly.
  const THREAD_ID = 'thread-run-prepend';
  function items(): Item[] {
    const built: Item[] = [prose('p0', 0, THREAD_ID)];
    for (let i = 0; i < RUN_ROWS; i += 1) built.push(tool(`a${i}`, i + 1, THREAD_ID));
    built.push(prose('p1', RUN_ROWS + 1, THREAD_ID));
    built.push(tool('b0', RUN_ROWS + 2, THREAD_ID));
    return built;
  }

  it('holds the reading position when an earlier chunk mounts', async () => {
    const { scrollEl } = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);
    const runs = scrollEl.querySelectorAll('[data-testid="activity-run"]');
    expect(runs).toHaveLength(2);
    const clip = runs[0].querySelector('[data-testid="activity-run-clip"]') as HTMLElement;

    // Vacuity guard: with the cap not engaged there is no scroll position to
    // hold, and the compensation would clamp to zero either way.
    expect(clip.scrollHeight).toBeGreaterThan(clip.clientHeight);

    // The reader has scrolled to the run's head, where the boundary is.
    clip.scrollTop = 0;
    await raf();
    const anchor = `a${RUN_ROWS - WINDOW_ROWS}`;
    const anchorBefore = offsetInClip(clip, anchor);
    const heightBefore = clip.scrollHeight;
    const clipHeightBefore = clip.clientHeight;
    const outerBefore = scrollEl.scrollTop;
    const outerHeightBefore = scrollEl.scrollHeight;

    (runs[0].querySelector('[data-testid="activity-run-earlier"]') as HTMLElement).click();
    await tick();
    await raf();

    // The rows really did arrive above the anchor — without this the drift
    // assertion below would pass on a no-op.
    expect(clip.querySelector('[data-item-id="a0"]')).not.toBeNull();
    const grew = clip.scrollHeight - heightBefore;
    expect(grew).toBeGreaterThan(0);
    // WebKit has no `overflow-anchor`, so the compensation is the whole
    // mechanism: scrollTop absorbs exactly what was prepended.
    expect(clip.scrollTop).toBe(grew);
    expect(Math.abs(offsetInClip(clip, anchor) - anchorBefore)).toBeLessThanOrEqual(DRIFT_PX);

    // And the cap means the timeline outside the run does not move either:
    // ten more rows of DOM, same row height, same page.
    expect(clip.clientHeight).toBe(clipHeightBefore);
    expect(scrollEl.scrollHeight).toBe(outerHeightBefore);
    expect(scrollEl.scrollTop).toBe(outerBefore);
  });
});

describe('activity run — jump lands on the target', () => {
  const THREAD_ID = 'thread-run-jump';
  // Two windows long, and the target sits half a window past the run's head:
  // off-window (the tail window is the last WINDOW_ROWS rows) with room on both
  // sides, so the centering is unclamped. A target nearer the head is CORRECTLY
  // not centered — there is nothing above it to center against — which is a
  // property of the math, covered where the math lives.
  const JUMP_ROWS = WINDOW_ROWS * 2;
  const TARGET = `a${WINDOW_ROWS - 10}`;
  function items(): Item[] {
    const built: Item[] = [prose('p0', 0, THREAD_ID)];
    for (let i = 0; i < JUMP_ROWS; i += 1) built.push(tool(`a${i}`, i + 1, THREAD_ID));
    return built;
  }

  it('centers an item that was outside the mount window, and keeps it there', async () => {
    const { scrollEl, pane } = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);
    const clip = scrollEl.querySelector('[data-testid="activity-run-clip"]') as HTMLElement;
    // Precondition: the target is genuinely off-window, so the jump has to
    // relocate the window AND scroll — not merely scroll.
    expect(clip.querySelector(`[data-item-id="${TARGET}"]`)).toBeNull();

    pane.requestScrollToItem(TARGET, { flash: true });
    await waitFor(
      () => clip.querySelector(`[data-item-id="${TARGET}"]`) !== null,
      'jump target to mount inside the run',
    );
    await raf();

    expect(isFullyVisible(clip, TARGET)).toBe(true);
    // Centered, not merely scrolled into view: the reader needs the rows that
    // led to the hit as much as the hit.
    const row = runRow(clip, TARGET).getBoundingClientRect();
    const rowCenter = (row.top + row.bottom) / 2 - clip.getBoundingClientRect().top;
    expect(Math.abs(rowCenter - clip.clientHeight / 2)).toBeLessThanOrEqual(4);

    // This is the LIVE run, so its controller is what would otherwise drag
    // the reader back to the tail. The jump escapes bottom-follow explicitly
    // (a zero-width scrollbar makes the package's geometric probe impossible),
    // and that has to hold across the spring's settle window, not just the
    // frame after the write.
    for (let i = 0; i < 30; i += 1) await raf();
    expect(isFullyVisible(clip, TARGET)).toBe(true);
  });
});
