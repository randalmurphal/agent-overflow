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
  userScrollTo,
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

    // The reader scrolls up inside the run, then collapses it.
    clip.scrollTop = 0;
    await raf();
    (run.querySelector('[data-testid="activity-run-rail"]') as HTMLElement).click();
    await tick();
    await raf();
    expect(run.querySelector('[data-testid="activity-run-clip"]')).toBeNull();

    (run.querySelector('[data-testid="activity-run-header"]') as HTMLElement).click();
    await tick();
    await raf();

    // The offset they were reading at is gone with the clip it belonged to,
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

describe('activity run — the tail window slides', () => {
  // The other edge of the arithmetic above. A tail-following window starts at
  // `children.length - rows`, so every appended row drops one from the head —
  // and where a prepend is compensated, that drop was not. The content shrinks
  // by the dropped row and grows by the new one, so there is almost no scroll
  // delta left for the spring to animate; instead every row the reader is
  // watching moves up by a row height in a single frame.
  //
  // Which is why a streaming run glides right up until it reaches
  // `activityRunWindowRows` members and then starts teleporting.
  const THREAD_ID = 'thread-run-slide';
  function items(): Item[] {
    const built: Item[] = [prose('p0', 0, THREAD_ID)];
    for (let i = 0; i < RUN_ROWS; i += 1) built.push(tool(`a${i}`, i + 1, THREAD_ID));
    return built;
  }

  it('holds the rows already on screen when an appended row slides it', async () => {
    const { pane, scrollEl } = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);
    const run = scrollEl.querySelector('[data-testid="activity-run"]') as HTMLElement;
    // The LIVE run: the one whose growth the reader is watching arrive, and the
    // only one whose window slides.
    expect(run.dataset.live).toBe('true');
    const clip = run.querySelector('[data-testid="activity-run-clip"]') as HTMLElement;
    // Vacuity guards: the window must already be past its head (or nothing
    // drops), and the clip must be scrollable (or nothing can displace).
    expect(run.querySelector('[data-testid="activity-run-earlier"]')).not.toBeNull();
    expect(clip.scrollHeight).toBeGreaterThan(clip.clientHeight);

    const anchor = `a${RUN_ROWS - 1}`;
    const anchorBefore = offsetInClip(clip, anchor);

    pane.upsertItem(tool(`a${RUN_ROWS}`, RUN_ROWS + 1, THREAD_ID));
    // One flush, no frame: the spring runs on the next one, so this measures
    // the layout change itself rather than where the spring later put it.
    await tick();

    // The head really did drop — without this the assertion below passes on a
    // window that never moved.
    expect(clip.querySelector(`[data-item-id="a${RUN_ROWS - WINDOW_ROWS}"]`)).toBeNull();
    // The row the reader was looking at is still under their eye. Uncompensated
    // it has jumped up by the dropped row's height, and no spring covers it
    // because the content barely changed size.
    expect(Math.abs(offsetInClip(clip, anchor) - anchorBefore)).toBeLessThanOrEqual(DRIFT_PX);
  });

  it('glides over the appended row instead of arriving at it', async () => {
    // The other half of the same fix, and the half the reader actually
    // reported. Holding the anchor is what removes the jump; it also opens a
    // gap at the clip's bottom exactly one row tall, and NOTHING in the
    // controller's own observers can see it — the content's total height barely
    // changed, so a delta-driven follow has nothing to chase and the run would
    // simply sit a row short of its newest activity, further short on every
    // append after that.
    const { pane, scrollEl } = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);
    const run = scrollEl.querySelector('[data-testid="activity-run"]') as HTMLElement;
    const clip = run.querySelector('[data-testid="activity-run-clip"]') as HTMLElement;
    expect(run.querySelector('[data-testid="activity-run-earlier"]')).not.toBeNull();
    const bottomGap = () => clip.scrollHeight - clip.scrollTop - clip.clientHeight;
    expect(bottomGap()).toBeLessThanOrEqual(DRIFT_PX);

    pane.upsertItem(tool(`a${RUN_ROWS}`, RUN_ROWS + 1, THREAD_ID));
    await tick();

    // The gap the compensation opened: a whole row, not a rounding error. This
    // is what proves the samples below are a glide over real distance.
    const opened = bottomGap();
    expect(opened).toBeGreaterThan(8);

    const gaps: number[] = [];
    for (let i = 0; i < 40 && (i === 0 || gaps[gaps.length - 1] > DRIFT_PX); i += 1) {
      await raf();
      gaps.push(bottomGap());
    }

    // Closed, and closed gradually. A snap would land the whole row in one
    // frame and produce no sample in between.
    expect(gaps[gaps.length - 1]).toBeLessThanOrEqual(DRIFT_PX);
    const partial = gaps.filter((gap) => gap > DRIFT_PX && gap < opened - DRIFT_PX);
    expect(partial.length).toBeGreaterThanOrEqual(2);
    // Monotone: the run only ever closes on its newest row. A gap that grew
    // mid-glide would mean a second writer is moving the clip.
    for (let i = 1; i < gaps.length; i += 1) {
      expect(gaps[i]).toBeLessThanOrEqual(gaps[i - 1] + DRIFT_PX);
    }
  });

  it('does the same for every row after the first', async () => {
    // The compensation carries state across flushes — the head it last saw, and
    // the measurement it hands from the pre-flush half to the post-flush one. A
    // run streams dozens of rows, so the second append matters as much as the
    // first: a handoff left set, or a head left stale, shows up here and not in
    // a single-append test.
    const { pane, scrollEl } = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);
    const run = scrollEl.querySelector('[data-testid="activity-run"]') as HTMLElement;
    const clip = run.querySelector('[data-testid="activity-run-clip"]') as HTMLElement;
    const bottomGap = () => clip.scrollHeight - clip.scrollTop - clip.clientHeight;

    for (let n = 0; n < 4; n += 1) {
      const anchor = `a${RUN_ROWS + n - 1}`;
      const anchorBefore = offsetInClip(clip, anchor);

      pane.upsertItem(tool(`a${RUN_ROWS + n}`, RUN_ROWS + n + 1, THREAD_ID));
      await tick();

      // Same two claims as above, on every row: nothing under the reader
      // moved, and there is a row's worth of glide left to run.
      expect(Math.abs(offsetInClip(clip, anchor) - anchorBefore)).toBeLessThanOrEqual(DRIFT_PX);
      expect(bottomGap()).toBeGreaterThan(8);
      await waitFor(() => bottomGap() <= DRIFT_PX, `append ${n} to finish gliding`);
      // The window really is still sliding this deep in — otherwise the later
      // iterations are just the append case, which the prepend suite covers.
      expect(run.querySelector('[data-testid="activity-run-earlier"]')).not.toBeNull();
    }
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

    pane.requestScrollToItem(TARGET);
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

describe('activity run — a toggle opens upward', () => {
  // A run with prose BELOW it, so there is content whose position a collapse
  // or expand could disturb. The whole question this suite asks is which side
  // of the change absorbs it.
  const THREAD_ID = 'thread-run-toggle';
  // Scrollback ABOVE the run, and plenty: a collapse opens upward by taking
  // scrollTop back, and a run near the top of a short thread has nowhere to
  // take it from. That clamp is real (nothing can scroll above the first row)
  // but it is not what these assertions are about.
  const HEAD_PROSE = 12;
  const TAIL_PROSE = 8;
  function items(): Item[] {
    const built: Item[] = [];
    for (let i = 0; i < HEAD_PROSE; i += 1) built.push(prose(`h${i}`, i, THREAD_ID));
    for (let i = 0; i < RUN_ROWS; i += 1) {
      built.push(tool(`a${i}`, HEAD_PROSE + i, THREAD_ID));
    }
    for (let i = 0; i < TAIL_PROSE; i += 1) {
      built.push(prose(`t${i}`, HEAD_PROSE + RUN_ROWS + i, THREAD_ID));
    }
    return built;
  }

  function rowTop(scrollEl: HTMLElement, itemId: string): number {
    const row = scrollEl.querySelector(`[data-item-id="${itemId}"]`);
    if (!(row instanceof HTMLElement)) throw new Error(`row ${itemId} is not rendered`);
    return row.getBoundingClientRect().top - scrollEl.getBoundingClientRect().top;
  }

  /** The toggle plus the frames its converging restore needs. The header,
   * not the rail strip: the strip folds with the clip, so it cannot expand
   * a collapsed run — the header is the control present in both states. */
  async function toggleHeader(run: HTMLElement): Promise<void> {
    (run.querySelector('[data-testid="activity-run-header"]') as HTMLElement).click();
    await tick();
    await raf();
    await raf();
    await raf();
  }

  it('holds the rows below a run still across collapse and expand', async () => {
    const { scrollEl } = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);
    const run = scrollEl.querySelector('[data-testid="activity-run"]') as HTMLElement;

    // Put the run at the top of the viewport, so the prose after it fills the
    // rest and the anchor the transaction picks is genuinely BELOW the change.
    // A real reader scroll, wheel and all: a bare `scrollTop` write leaves the
    // controller sticky, and a sticky transaction correctly re-pins the bottom
    // instead of holding a row — which is the other half of this feature, and
    // tested as such below.
    await userScrollTo(
      scrollEl,
      scrollEl.scrollTop + run.getBoundingClientRect().top
        - scrollEl.getBoundingClientRect().top,
    );
    await raf();

    const runHeightBefore = run.getBoundingClientRect().height;
    const proseBefore = rowTop(scrollEl, 't0');
    expect(proseBefore).toBeGreaterThan(0);

    await toggleHeader(run);

    // Vacuity guard: the run really did shrink, by much more than the drift
    // this assertion tolerates. Without the anchor the prose would have risen
    // by exactly this much.
    const collapsedBy = runHeightBefore - run.getBoundingClientRect().height;
    expect(collapsedBy).toBeGreaterThan(100);
    expect(Math.abs(rowTop(scrollEl, 't0') - proseBefore)).toBeLessThanOrEqual(DRIFT_PX);

    await toggleHeader(run);

    // And expanding gives the height back above the reader rather than pushing
    // them down the page.
    expect(run.getBoundingClientRect().height).toBeCloseTo(runHeightBefore, 0);
    expect(Math.abs(rowTop(scrollEl, 't0') - proseBefore)).toBeLessThanOrEqual(DRIFT_PX);
  });

  it('stays pinned to the bottom through a bulk toggle, without a spring', async () => {
    // Mounting settles at the bottom, which is where the spring would otherwise
    // engage: the growth reaches the controller as "content grew while sticky"
    // and it animates across the whole delta. Two frames is far inside any
    // spring — reaching the bottom that fast IS the assertion.
    const { pane, scrollEl } = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);
    const bottomGap = () => scrollEl.scrollHeight - scrollEl.scrollTop - scrollEl.clientHeight;
    expect(bottomGap()).toBeLessThanOrEqual(2);
    const heightBefore = scrollEl.scrollHeight;

    pane.activityRuns.setAllCollapsed(true);
    await tick();
    await raf();
    await raf();
    expect(scrollEl.scrollHeight).toBeLessThan(heightBefore);
    expect(bottomGap()).toBeLessThanOrEqual(2);

    pane.activityRuns.setAllCollapsed(false);
    await tick();
    await raf();
    await raf();
    expect(scrollEl.scrollHeight).toBeCloseTo(heightBefore, 0);
    expect(bottomGap()).toBeLessThanOrEqual(2);
  });
});

describe('activity run — a clamped mount restore is re-applied as content measures', () => {
  // The virtualizer remounts a run whose reader had scrolled inside it, and
  // the mount's restore write lands BEFORE the rows inside finish measuring —
  // markdown, payload bodies, and font metrics all land late in production.
  // The browser clamps the write toward 0, and an escaped run has no
  // controller or settle-follow to re-ask: it parked at its top, scrollable
  // with the fade off (the 2026-08-22 "no faded top edge" defect). The
  // restore observer must re-apply the armed target as the content grows.
  //
  // Late measurement is simulated by shrinking the run's thinking summaries
  // while the run is unmounted and restoring them after it remounts: same
  // signal path (the content ResizeObserver), deterministic heights.
  const THREAD_ID = 'thread-run-restore-hold';
  const LONG_THOUGHT = 'Considering the fixture layout in detail: the run needs interior height, '
    + 'so this reasoning row carries several clauses and wraps across multiple '
    + 'lines under the clip\'s narrow column, giving each row real height to lose.';
  const RUN_LEN = 20;
  const TAIL_PROSE = 20;

  function thinkingItem(id: string, index: number, summary: string, updatedAt = index): Item {
    return makeItem({
      id,
      threadId: THREAD_ID,
      itemIndex: index,
      kind: 'thinking',
      summary,
      createdAt: index,
      updatedAt,
    });
  }

  function items(): Item[] {
    const built: Item[] = [prose('p0', 0, THREAD_ID)];
    for (let i = 0; i < RUN_LEN; i += 1) {
      // Mostly thinking rows: their tail-clamped text is the height that can
      // shrink, and the clamp scenario needs the run to lose several hundred
      // px while unmounted.
      built.push(i % 10 === 0
        ? tool(`a${i}`, i + 1, THREAD_ID)
        : thinkingItem(`th${i}`, i + 1, LONG_THOUGHT));
    }
    for (let i = 0; i < TAIL_PROSE; i += 1) {
      // Tall rows: the run must end up past the virtualizer's above-viewport
      // buffer when the timeline rests at the bottom, or the return leg is
      // never a remount.
      built.push(makeItem({
        id: `r${i}`,
        threadId: THREAD_ID,
        itemIndex: RUN_LEN + 1 + i,
        summary: `Reply ${i}, paragraph one: enough prose that the row takes real height.\n\n`
          + 'Paragraph two keeps going so consecutive rows do not share one height bucket '
          + 'and the seeded scrollback genuinely exceeds the windowing buffers.\n\n'
          + 'Paragraph three exists purely for altitude.',
        createdAt: RUN_LEN + 1 + i,
        updatedAt: RUN_LEN + 1 + i,
      }));
    }
    return built;
  }

  const clipOf = (scrollEl: HTMLElement): HTMLElement | null =>
    scrollEl.querySelector('[data-testid="activity-run-clip"]');
  const fadeOf = (scrollEl: HTMLElement): HTMLElement | null =>
    scrollEl.querySelector('[data-testid="activity-run-top-fade"]');

  it('lands on the saved position once the rows have measured, fade on', async () => {
    const { pane, scrollEl } = await mountTimeline(THREAD_ID, items(), QUIET_BOTTOM);

    // The run sits at the head of a long thread; the mount lands at the
    // bottom, far below it. It must be OUT of the rendered window for the
    // return leg to be a real remount.
    await waitFor(() => clipOf(scrollEl) === null, 'run unmounted at the bottom');

    // Up to the run.
    await userScrollTo(scrollEl, 0);
    await waitFor(() => clipOf(scrollEl) !== null, 'run mounts on the way up');
    const clip = clipOf(scrollEl)!;
    await waitFor(() => clip.scrollHeight > clip.clientHeight, 'clip scrollable');

    // The reader steps inside: a wheel gesture (intent) plus the position.
    clip.dispatchEvent(new WheelEvent('wheel', { deltaY: -120, bubbles: true }));
    clip.scrollTop = Math.max(0, clip.scrollHeight - clip.clientHeight - 20);
    await raf();
    const target = clip.scrollTop;
    expect(target).toBeGreaterThan(250); // vacuity: deep enough that the shrink strands it

    // Away again, far enough that the run leaves the window.
    await userScrollTo(scrollEl, scrollEl.scrollHeight);
    await waitFor(() => clipOf(scrollEl) === null, 'run unmounts at the bottom');

    // While it is unmounted, its thinking rows lose their height — the
    // remount will measure SHORT, exactly like unresolved rows in production.
    for (let i = 0; i < RUN_LEN; i += 1) {
      if (i % 10 === 0) continue;
      pane.upsertItem(thinkingItem(`th${i}`, i + 1, 'hm', 1000 + i));
    }
    await tick();

    // Back up: the remount's restore write clamps against the short content.
    await userScrollTo(scrollEl, 0);
    await waitFor(() => clipOf(scrollEl) !== null, 'run remounts');
    const remounted = clipOf(scrollEl)!;
    await raf();
    // Vacuity guard: the clamp really happened — the saved position is not
    // reachable in the shrunken clip, or the whole scenario proved nothing.
    expect(remounted.scrollTop).toBeLessThan(target);

    // The rows measure in (grow back). The armed restore must follow the
    // growth to the saved position — this is the fix under test; before it,
    // the run parked wherever the clamp left it, scrollable with no fade.
    for (let i = 0; i < RUN_LEN; i += 1) {
      if (i % 10 === 0) continue;
      pane.upsertItem(thinkingItem(`th${i}`, i + 1, LONG_THOUGHT, 2000 + i));
    }
    await tick();
    const runEl = scrollEl.querySelector('[data-testid="activity-run"]') as HTMLElement;
    const runId = runEl?.dataset.runId ?? '?';
    const state = (): string => {
      const snap = pane.activityRuns.scrollSnapshot(runId);
      return `st=${remounted.scrollTop} sh=${remounted.scrollHeight} ch=${remounted.clientHeight}`
        + ` snap=${JSON.stringify(snap)} target=${target}`;
    };
    try {
      await waitFor(
        () => Math.abs(remounted.scrollTop - target) <= 2,
        'restore re-applied to the saved position',
      );
    } catch {
      throw new Error(`restore not re-applied [${state()}]`);
    }
    expect(fadeOf(scrollEl)?.dataset.faded).toBe('true');
  });
});
