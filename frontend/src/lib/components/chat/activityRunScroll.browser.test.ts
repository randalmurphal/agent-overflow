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

  // Driven by the scroll trigger rather than the boundary button, because in
  // real geometry that is the only way a scrollable run gets here: the
  // boundary sits at the very top of the content, so it becomes visible only
  // once the reader is inside the trigger runway. The button is the affordance
  // for the case the trigger deliberately refuses — a window whose rows all
  // fit under the cap, which never scrolls and shows its boundary outright.
  it('holds the reading position when an earlier chunk mounts', async () => {
    const { scrollEl } = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);
    const runs = scrollEl.querySelectorAll('[data-testid="activity-run"]');
    expect(runs).toHaveLength(2);
    const clip = runs[0].querySelector('[data-testid="activity-run-clip"]') as HTMLElement;

    // Vacuity guard: with the cap not engaged there is no scroll position to
    // hold, and the compensation would clamp to zero either way.
    expect(clip.scrollHeight).toBeGreaterThan(clip.clientHeight);

    // The reader scrolls to the run's head, which IS the trigger — a
    // scrollable run pages its next chunk in rather than making them ask.
    // The wheel is not decoration: paging arms on the gesture, so that a
    // position the run wrote itself can never be read as the reader arriving.
    clip.dispatchEvent(new WheelEvent('wheel', { deltaY: -120, bubbles: true }));
    clip.scrollTop = 0;
    // Measured synchronously, before the next rendering step dispatches the
    // scroll event that starts the mount.
    const anchor = `a${RUN_ROWS - WINDOW_ROWS}`;
    const anchorBefore = offsetInClip(clip, anchor);
    const heightBefore = clip.scrollHeight;
    const clipHeightBefore = clip.clientHeight;
    const outerBefore = scrollEl.scrollTop;
    const outerHeightBefore = scrollEl.scrollHeight;

    await raf();
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

  it('reopens a collapsed run on its newest row', async () => {
    const { scrollEl } = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);
    const run = scrollEl.querySelectorAll('[data-testid="activity-run"]')[0] as HTMLElement;
    const clip = run.querySelector('[data-testid="activity-run-clip"]') as HTMLElement;

    // Vacuity guard: with the cap not engaged, top and bottom are the same
    // position and every assertion below would hold either way.
    expect(clip.scrollHeight).toBeGreaterThan(clip.clientHeight);

    // The reader scrolls up inside the run, then collapses it to a chip.
    clip.scrollTop = 0;
    await raf();
    (run.querySelector('[data-testid="activity-run-rail"]') as HTMLElement).click();
    await tick();
    await raf();
    expect(run.querySelector('[data-testid="activity-run-clip"]')).toBeNull();

    (run.querySelector('[data-testid="activity-run-chip"]') as HTMLElement).click();
    await tick();
    await raf();

    // The offset they were reading at is gone with the chip that replaced it,
    // so the run opens where a never-scrolled one does — its newest activity.
    const reopened = run.querySelector('[data-testid="activity-run-clip"]') as HTMLElement;
    expect(reopened.scrollHeight).toBeGreaterThan(reopened.clientHeight);
    expect(reopened.scrollHeight - reopened.scrollTop - reopened.clientHeight)
      .toBeLessThanOrEqual(DRIFT_PX);
  });

  it('reopens on its newest row after the header collapses every run', async () => {
    // The header's bulk control, not the rail: it flips the THREAD's default
    // rather than one run's override, and every mounted run reopens at once
    // from a single flush. That is the path the reader actually uses.
    const { pane, scrollEl } = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);
    const run = scrollEl.querySelectorAll('[data-testid="activity-run"]')[0] as HTMLElement;
    expect((run.querySelector('[data-testid="activity-run-clip"]') as HTMLElement).scrollHeight)
      .toBeGreaterThan(
        (run.querySelector('[data-testid="activity-run-clip"]') as HTMLElement).clientHeight,
      );

    pane.activityRuns.setAllCollapsed(true);
    await tick();
    await raf();
    expect(run.querySelector('[data-testid="activity-run-clip"]')).toBeNull();

    pane.activityRuns.setAllCollapsed(false);
    await tick();
    await raf();
    await raf();

    const reopened = run.querySelector('[data-testid="activity-run-clip"]') as HTMLElement;
    expect(reopened.scrollHeight).toBeGreaterThan(reopened.clientHeight);
    expect(reopened.scrollHeight - reopened.scrollTop - reopened.clientHeight)
      .toBeLessThanOrEqual(DRIFT_PX);
  });

  it('stays on the newest row as the run grows under it', async () => {
    // The position write happens once, when the clip mounts — but the rows
    // inside are not done then: payload bodies resolve, spans land, a leased
    // expansion remounts open. Without the settle observer the clip keeps the
    // offset it was given and every one of those leaves the reader partway up
    // a run they just opened, which is what expanding several at once showed.
    const { scrollEl } = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);
    const run = scrollEl.querySelectorAll('[data-testid="activity-run"]')[0] as HTMLElement;
    const clip = run.querySelector('[data-testid="activity-run-clip"]') as HTMLElement;
    const content = clip.firstElementChild as HTMLElement;
    expect(clip.scrollHeight).toBeGreaterThan(clip.clientHeight);

    // Growth from inside the clip, the way a settling row produces it — not a
    // scroll, so nothing but the observer can notice it.
    const grown = document.createElement('div');
    grown.style.height = '400px';
    content.appendChild(grown);
    // Two frames: the observer delivers before the next rAF, and the write it
    // makes needs the one after to be readable as a settled position.
    await raf();
    await raf();

    expect(clip.scrollHeight - clip.scrollTop - clip.clientHeight)
      .toBeLessThanOrEqual(DRIFT_PX);

    // And a reader who is NOT at the bottom keeps their place through the same
    // growth — following is for the position where it is what they are looking
    // at, not a rule about the run.
    //
    // Mid-run rather than the top, so the position under test is unambiguously
    // a reading position: the top doubles as the paging trigger's zone.
    //
    // The wheel is what makes this a READER leaving the last row. Growth alone
    // moves the bottom away from the clip every time a row resolves, so a run
    // that read "not at the bottom any more" off the geometry would abandon the
    // follow on the first one — the bug this observer exists to fix.
    const mid = Math.round((clip.scrollHeight - clip.clientHeight) / 2);
    expect(mid).toBeGreaterThan(0);
    clip.dispatchEvent(new WheelEvent('wheel', { deltaY: -120, bubbles: true }));
    clip.scrollTop = mid;
    await raf();
    const second = document.createElement('div');
    second.style.height = '400px';
    content.appendChild(second);
    await raf();
    await raf();

    expect(clip.scrollTop).toBe(mid);
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
