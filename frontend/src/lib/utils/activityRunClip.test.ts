import { describe, expect, it } from 'vitest';
import {
  ACTIVITY_RUN_CAP_CSS,
  ACTIVITY_RUN_CAP_REM,
  ACTIVITY_RUN_CAP_REM_PX,
  ACTIVITY_RUN_CAP_ROWS,
  ACTIVITY_RUN_ROW_REM,
  activityRunClipMaxHeight,
  activityRunDisclosureBodies,
  activityRunExpandedHeight,
  activityRunRecordCollapsedHeights,
  activityRunRowViewportTop,
  activityRunScrollTopHoldingRow,
  activityRunShouldMountEarlier,
} from './activityRunClip';

// Which bodies lift a run's cap, and by how much. happy-dom reports zero
// geometry, so the heights here are stamped explicitly and the REAL cap
// arithmetic is asserted in activityRunClip.browser.test.ts; what this file
// covers is the selection, which is pure DOM structure: a body outside the run
// must not lift this run's cap, and a nested one must not be counted twice.

function clipWith(html: string): HTMLElement {
  const clip = document.createElement('div');
  clip.innerHTML = html;
  return clip;
}

function disclosure(bodyId: string, expanded: boolean): string {
  return `<button aria-expanded="${expanded}" aria-controls="${bodyId}"></button>`;
}

/** happy-dom lays nothing out, so a body's contribution is stamped on it. */
function stampHeight(body: HTMLElement, px: number): HTMLElement {
  Object.defineProperty(body, 'offsetHeight', { value: px, configurable: true });
  return body;
}

describe('the cap', () => {
  it('is the row count it claims to be, in rem', () => {
    // The row count is the tunable; the height is derived from it. Asserted
    // because a cap edited as a bare rem value would silently stop meaning
    // "this many rows", which is the only thing it is FOR.
    expect(ACTIVITY_RUN_CAP_REM).toBe(ACTIVITY_RUN_CAP_ROWS * ACTIVITY_RUN_ROW_REM);
    expect(ACTIVITY_RUN_CAP_CSS).toBe(`min(50vh, ${ACTIVITY_RUN_CAP_REM}rem)`);
  });

  it('keeps its px mirror in step, so placement estimates cannot drift', () => {
    expect(ACTIVITY_RUN_CAP_REM_PX).toBe(ACTIVITY_RUN_CAP_REM * 16);
  });
});

describe('holding a row across a window slide', () => {
  // The arithmetic needs real geometry and is asserted in
  // activityRunScroll.browser.test.ts. What matters here is the ONE answer that
  // is pure structure: a row the caller cannot find. Both callers treat null as
  // "no shared row to hold, so leave the position to whoever owns it" — a jump
  // that relocated the window wholesale, or a clip that just remounted. A zero
  // in its place would compensate against a row that was never there and drag
  // the reader to the top of the run.
  it('reports no answer for a row outside the mount window', () => {
    const clip = clipWith('<div data-run-child="12"></div>');
    expect(activityRunRowViewportTop(clip, 40)).toBeNull();
    expect(activityRunScrollTopHoldingRow(clip, 40, 80)).toBeNull();
  });

  it('answers for a row that is mounted', () => {
    // happy-dom lays nothing out, so every rect is zero — the value is a
    // vacuous 0 and only its presence is meaningful. Asserted so the null above
    // is known to be the missing-row branch rather than the only branch.
    const clip = clipWith('<div data-run-child="12"></div>');
    expect(activityRunRowViewportTop(clip, 12)).not.toBeNull();
    expect(activityRunScrollTopHoldingRow(clip, 12, 0)).not.toBeNull();
  });
});

describe('activityRunShouldMountEarlier', () => {
  /** A clip showing 300 of 1500px, scrolled to `scrollTop`. */
  function scrolled(scrollTop: number) {
    return { scrollTop, clientHeight: 300, scrollHeight: 1500 };
  }

  it('pages in when the reader reaches the top and more is hidden', () => {
    expect(activityRunShouldMountEarlier(scrolled(0), 170)).toBe(true);
  });

  it('pages in from a runway above the top, not only at exactly zero', () => {
    // Otherwise the rows arrive after the reader has already met the boundary.
    expect(activityRunShouldMountEarlier(scrolled(60), 170)).toBe(true);
  });

  it('stays out of it mid-scroll', () => {
    expect(activityRunShouldMountEarlier(scrolled(900), 170)).toBe(false);
  });

  it('stays out of it with nothing hidden above', () => {
    expect(activityRunShouldMountEarlier(scrolled(0), 0)).toBe(false);
  });

  it('refuses a window that already fits under the cap', () => {
    // The guard that keeps this from overriding `activityRunWindowRows`: such a
    // window rests inside the trigger zone because it never scrolls, and
    // without the check it would page chunk after chunk in at mount time until
    // the content overflowed.
    expect(activityRunShouldMountEarlier({ scrollTop: 0, clientHeight: 300, scrollHeight: 300 }, 170))
      .toBe(false);
    // Overflowing by less than the runway is the same case.
    expect(activityRunShouldMountEarlier({ scrollTop: 40, clientHeight: 300, scrollHeight: 340 }, 170))
      .toBe(false);
  });
});

describe('activityRunClipMaxHeight', () => {
  it('is the bare cap with nothing expanded', () => {
    expect(activityRunClipMaxHeight(0)).toBe(ACTIVITY_RUN_CAP_CSS);
  });

  it('adds exactly what expansion asked for', () => {
    expect(activityRunClipMaxHeight(220)).toBe(`calc(${ACTIVITY_RUN_CAP_CSS} + 220px)`);
    // Fractional heights are real (borders, DPR); the style value stays integral
    // so a sub-pixel jitter cannot re-write the declaration every frame.
    expect(activityRunClipMaxHeight(220.4)).toBe(`calc(${ACTIVITY_RUN_CAP_CSS} + 220px)`);
  });
});

describe('activityRunDisclosureBodies', () => {
  it('finds the body an expanded disclosure points at', () => {
    const clip = clipWith(`${disclosure('body-1', true)}<div id="body-1"></div>`);

    expect(activityRunDisclosureBodies(clip).expanded.map((b) => b.id)).toEqual(['body-1']);
  });

  it('classifies a collapsed disclosure body as collapsed, not expanded', () => {
    // The always-mounted kind: its collapsed height is the baseline the cap
    // subtracts once the reader expands it.
    const clip = clipWith(`${disclosure('body-1', false)}<div id="body-1"></div>`);

    const bodies = activityRunDisclosureBodies(clip);
    expect(bodies.expanded).toEqual([]);
    expect(bodies.collapsed.map((b) => b.id)).toEqual(['body-1']);
  });

  it('ignores an aria-controls trigger without a disclosure state', () => {
    // A combobox or menu trigger also carries aria-controls; without a
    // true/false aria-expanded it is not a disclosure over a body.
    const clip = clipWith('<button aria-controls="body-1"></button><div id="body-1"></div>');

    const bodies = activityRunDisclosureBodies(clip);
    expect(bodies.expanded).toEqual([]);
    expect(bodies.collapsed).toEqual([]);
  });

  it('ignores a body outside this run', () => {
    // The id resolves through the clip, never through `document` — otherwise
    // one run's open diff would lift every other run's cap.
    const outside = document.createElement('div');
    outside.id = 'body-elsewhere';
    document.body.appendChild(outside);
    const clip = clipWith(disclosure('body-elsewhere', true));

    expect(activityRunDisclosureBodies(clip).expanded).toEqual([]);
    outside.remove();
  });

  it('ignores a stale pointer at an unmounted body', () => {
    const clip = clipWith(disclosure('body-gone', true));

    const bodies = activityRunDisclosureBodies(clip);
    expect(bodies.expanded).toEqual([]);
    expect(bodies.collapsed).toEqual([]);
  });

  it('counts a body once when two disclosures point at it', () => {
    const clip = clipWith(
      `${disclosure('body-1', true)}${disclosure('body-1', true)}<div id="body-1"></div>`,
    );

    expect(activityRunDisclosureBodies(clip).expanded.map((b) => b.id)).toEqual(['body-1']);
  });

  it('reads a body as expanded when any of its triggers says so', () => {
    // Transient double-trigger states resolve to the answer the reader acted
    // on; a body cannot be both, and expanded is the one that costs height.
    const clip = clipWith(
      `${disclosure('body-1', false)}${disclosure('body-1', true)}<div id="body-1"></div>`,
    );

    const bodies = activityRunDisclosureBodies(clip);
    expect(bodies.expanded.map((b) => b.id)).toEqual(['body-1']);
    expect(bodies.collapsed).toEqual([]);
  });

  it('drops a body nested inside another expanded body', () => {
    // A tool row expanded inside an expanded subagent card: the card's height
    // already includes it, so counting both would lift the cap twice for one
    // expansion.
    const clip = clipWith(`
      ${disclosure('card', true)}
      <div id="card">
        ${disclosure('inner', true)}
        <div id="inner"></div>
      </div>
    `);

    expect(activityRunDisclosureBodies(clip).expanded.map((b) => b.id)).toEqual(['card']);
  });
});

describe('activityRunExpandedHeight', () => {
  it('sums whole heights for mount-on-expand bodies', () => {
    // Nothing was on screen while collapsed, so the whole height is what
    // expansion added.
    const first = stampHeight(document.createElement('div'), 120);
    const second = stampHeight(document.createElement('div'), 80);

    expect(activityRunExpandedHeight([first, second])).toBe(200);
  });

  it('is zero with nothing expanded', () => {
    expect(activityRunExpandedHeight([])).toBe(0);
  });

  it('subtracts the height a body already had while collapsed', () => {
    // The always-mounted kind: its preview was on screen inside the base
    // cap's budget before the reader expanded it, so only the growth lifts
    // the cap.
    const body = stampHeight(document.createElement('div'), 60);
    activityRunRecordCollapsedHeights([body]);
    stampHeight(body, 200);

    expect(activityRunExpandedHeight([body])).toBe(140);
  });

  it('never contributes negatively', () => {
    // An expanded body can measure SHORTER than its recorded collapsed
    // height for a frame (content swapped before layout settles); the cap
    // must not shrink below its base for it.
    const body = stampHeight(document.createElement('div'), 60);
    activityRunRecordCollapsedHeights([body]);
    stampHeight(body, 40);

    expect(activityRunExpandedHeight([body])).toBe(0);
  });

  it('re-records a collapsed preview that grew before expansion', () => {
    // A reasoning tail fills its clamp while collapsed; only the latest
    // collapsed height is what expansion replaces.
    const body = stampHeight(document.createElement('div'), 20);
    activityRunRecordCollapsedHeights([body]);
    stampHeight(body, 58);
    activityRunRecordCollapsedHeights([body]);
    stampHeight(body, 200);

    expect(activityRunExpandedHeight([body])).toBe(142);
  });
});
