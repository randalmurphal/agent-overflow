// The run map's scroll arithmetic (RUN-MAP §9), tested directly.
//
// The controller's suite proves the DECISIONS these feed — when a glide runs,
// what the chip says, which compensation write lands. This file proves the
// arithmetic itself, and it exists mostly for `pickAnchor`: the descent rule is
// the one non-obvious answer on this surface, and through the controller it can
// only ever be observed as a compensation number, which is two rules at once.
//
// happy-dom lays nothing out, so every box states its own rect.

import { describe, expect, it } from 'vitest';
import {
  BAND_BOTTOM_FRACTION,
  BAND_REST_FRACTION,
  BAND_TOP_FRACTION,
  canScroll,
  firstReaching,
  foldAnimates,
  hasSelectionInside,
  inBand,
  isOffscreen,
  maxScrollTop,
  pickAnchor,
  restingScrollTop,
  targetOffset,
} from './runMapGeometry';

const SCROLLER_TOP = 100;
const CLIENT_HEIGHT = 400;
const SCROLL_HEIGHT = 2000;

function rect(top: number, height: number): DOMRect {
  return {
    x: 0, y: top, top, bottom: top + height, left: 0, right: 600,
    width: 600, height, toJSON: () => ({}),
  } as DOMRect;
}

/** A box that states its own viewport rect, appended nowhere by default. */
function box(top: number, height: number): HTMLElement {
  const el = document.createElement('div');
  el.getBoundingClientRect = () => rect(top, height);
  return el;
}

function makeScroller(
  options: { clientHeight?: number; scrollHeight?: number; scrollTop?: number } = {},
): HTMLElement {
  const el = document.createElement('div');
  el.getBoundingClientRect = () => rect(SCROLLER_TOP, options.clientHeight ?? CLIENT_HEIGHT);
  Object.defineProperty(el, 'clientHeight', {
    get: () => options.clientHeight ?? CLIENT_HEIGHT,
    configurable: true,
  });
  Object.defineProperty(el, 'scrollHeight', {
    get: () => options.scrollHeight ?? SCROLL_HEIGHT,
    configurable: true,
  });
  el.scrollTop = options.scrollTop ?? 0;
  return el;
}

describe('range arithmetic', () => {
  it('maxScrollTop is the overflow, and never negative', () => {
    expect(maxScrollTop(makeScroller())).toBe(SCROLL_HEIGHT - CLIENT_HEIGHT);
    expect(maxScrollTop(makeScroller({ scrollHeight: 100 }))).toBe(0);
  });

  it('canScroll is false for a surface that fits its content', () => {
    expect(canScroll(makeScroller())).toBe(true);
    expect(canScroll(makeScroller({ scrollHeight: CLIENT_HEIGHT }))).toBe(false);
  });

  it('targetOffset is measured from the scroller origin, not the page', () => {
    expect(targetOffset(makeScroller(), box(SCROLLER_TOP + 250, 40))).toBe(250);
    expect(targetOffset(makeScroller(), box(SCROLLER_TOP - 60, 40))).toBe(-60);
  });
});

describe('the band', () => {
  const scroller = () => makeScroller();

  it('holds a target between the two fractions of the viewport height', () => {
    const top = CLIENT_HEIGHT * BAND_TOP_FRACTION;
    const bottom = CLIENT_HEIGHT * BAND_BOTTOM_FRACTION;
    expect(inBand(scroller(), box(SCROLLER_TOP + top, 40))).toBe(true);
    expect(inBand(scroller(), box(SCROLLER_TOP + bottom, 40))).toBe(true);
    expect(inBand(scroller(), box(SCROLLER_TOP + top - 1, 40))).toBe(false);
    expect(inBand(scroller(), box(SCROLLER_TOP + bottom + 1, 40))).toBe(false);
  });

  it('parks the target on the rest line, clamped to the scrollable range', () => {
    const el = makeScroller({ scrollTop: 300 });
    // 300 (current) + 250 (offset) − rest line.
    expect(restingScrollTop(el, box(SCROLLER_TOP + 250, 40)))
      .toBe(300 + 250 - CLIENT_HEIGHT * BAND_REST_FRACTION);
    // A target near the document top cannot pull the viewport above zero.
    expect(restingScrollTop(makeScroller(), box(SCROLLER_TOP, 40))).toBe(0);
    // Nor past the end of the range.
    expect(restingScrollTop(makeScroller({ scrollTop: 1500 }), box(SCROLLER_TOP + 380, 40)))
      .toBe(SCROLL_HEIGHT - CLIENT_HEIGHT);
  });

  it('calls a target off-screen only once it clears the viewport entirely', () => {
    const el = makeScroller();
    expect(isOffscreen(el, box(SCROLLER_TOP + 200, 40))).toBe(false);
    // Bottom edge exactly on the viewport top: gone.
    expect(isOffscreen(el, box(SCROLLER_TOP - 40, 40))).toBe(true);
    // Top edge exactly on the viewport bottom: not yet arrived.
    expect(isOffscreen(el, box(SCROLLER_TOP + CLIENT_HEIGHT, 40))).toBe(true);
    // Straddling the bottom edge is still on screen.
    expect(isOffscreen(el, box(SCROLLER_TOP + CLIENT_HEIGHT - 1, 40))).toBe(false);
  });
});

// §9.8. The rule is a SCROLL rule: a 200ms height transition changes the
// document for 199ms after the anchor compensation that cancels it was
// measured, so an off-screen fold has to land whole, inside the hold.
describe('foldAnimates', () => {
  it('animates a region on screen and applies an off-screen one instantly', () => {
    const el = makeScroller();
    expect(foldAnimates(el, box(SCROLLER_TOP + 200, 40), false)).toBe(true);
    // Above the viewport — the auto-fold-above-a-reader case exactly.
    expect(foldAnimates(el, box(SCROLLER_TOP - 400, 40), false)).toBe(false);
    // Below it.
    expect(foldAnimates(el, box(SCROLLER_TOP + CLIENT_HEIGHT + 10, 40), false)).toBe(false);
    // Straddling either edge is on screen, so the reader would see the snap.
    expect(foldAnimates(el, box(SCROLLER_TOP - 20, 40), false)).toBe(true);
    expect(foldAnimates(el, box(SCROLLER_TOP + CLIENT_HEIGHT - 1, 40), false)).toBe(true);
  });

  it('reduced motion wins over position — instant either way', () => {
    const el = makeScroller();
    expect(foldAnimates(el, box(SCROLLER_TOP + 200, 40), true)).toBe(false);
    expect(foldAnimates(null, null, true)).toBe(false);
  });

  it('answers "animate" when it cannot measure — the harmless side of the miss', () => {
    // A visible fold that should have been instant is cosmetic; an off-screen
    // fold treated as visible is the viewport drift this rule exists to stop.
    expect(foldAnimates(null, box(0, 40), false)).toBe(true);
    expect(foldAnimates(makeScroller(), null, false)).toBe(true);
  });
});

describe('hasSelectionInside', () => {
  it('is false with no selection, and true for a range inside the scroller', () => {
    const el = document.createElement('div');
    const inner = document.createElement('p');
    inner.textContent = 'selected text';
    el.appendChild(inner);
    document.body.appendChild(el);
    const outside = document.createElement('p');
    outside.textContent = 'elsewhere';
    document.body.appendChild(outside);

    try {
      const selection = window.getSelection();
      selection?.removeAllRanges();
      expect(hasSelectionInside(el)).toBe(false);

      const range = document.createRange();
      range.selectNodeContents(inner);
      selection?.addRange(range);
      expect(hasSelectionInside(el)).toBe(true);

      // A selection somewhere else in the document holds nothing here.
      selection?.removeAllRanges();
      const away = document.createRange();
      away.selectNodeContents(outside);
      selection?.addRange(away);
      expect(hasSelectionInside(el)).toBe(false);
    } finally {
      window.getSelection()?.removeAllRanges();
      document.body.innerHTML = '';
    }
  });
});

describe('pickAnchor — the descent', () => {
  /** Wrap `child` in `depth` containers that each SPAN the whole document. */
  function span(child: HTMLElement, depth: number): HTMLElement {
    let inner = child;
    for (let i = 0; i < depth; i++) {
      const outer = document.createElement('div');
      outer.getBoundingClientRect = () => rect(SCROLLER_TOP - 500, 3000);
      outer.appendChild(inner);
      inner = outer;
    }
    return inner;
  }

  it('picks the first child whose box reaches the viewport top line', () => {
    const scroller = makeScroller();
    const above = box(SCROLLER_TOP - 200, 150); // bottom is above the line
    const straddling = box(SCROLLER_TOP - 20, 80);
    const below = box(SCROLLER_TOP + 100, 80);
    scroller.append(above, straddling, below);

    expect(firstReaching(scroller, SCROLLER_TOP)).toBe(straddling);
    expect(pickAnchor(scroller)).toBe(straddling);
  });

  // The whole point of the rule: every ancestor CONTAINS what grew above the
  // anchor, so its own top edge does not move. Stopping at one measures zero
  // and compensates nothing — silently, which is how this behaved before the
  // descent existed.
  it('descends past containers that span the growth to the row itself', () => {
    const scroller = makeScroller();
    const row = box(SCROLLER_TOP - 10, 60);
    scroller.appendChild(span(row, 8));

    expect(pickAnchor(scroller)).toBe(row);
  });

  it('descends into the row when the row itself is not a leaf', () => {
    const scroller = makeScroller();
    const row = box(SCROLLER_TOP - 10, 60);
    const glyph = box(SCROLLER_TOP - 200, 10); // entirely above the line
    const label = box(SCROLLER_TOP - 10, 20);
    row.append(glyph, label);
    scroller.appendChild(span(row, 3));

    expect(pickAnchor(scroller)).toBe(label);
  });

  it('stops at the deepest element that still reaches the line', () => {
    const scroller = makeScroller();
    const row = box(SCROLLER_TOP - 10, 60);
    // Every child of `row` sits entirely above the line, so `row` itself is
    // the deepest thing whose edge moves with the growth.
    row.append(box(SCROLLER_TOP - 300, 20), box(SCROLLER_TOP - 250, 20));
    scroller.appendChild(row);

    expect(pickAnchor(scroller)).toBe(row);
  });

  // A degenerate over-scroll — nothing reaches the line at all. Holding the
  // first child keeps the document put; holding nothing compensates nothing.
  it('falls back to the first child when everything sits above the line', () => {
    const scroller = makeScroller();
    const first = box(SCROLLER_TOP - 900, 100);
    scroller.append(first, box(SCROLLER_TOP - 700, 100));

    expect(pickAnchor(scroller)).toBe(first);
  });

  it('is null for an empty scroller rather than throwing', () => {
    expect(pickAnchor(makeScroller())).toBeNull();
  });

  // The descent cap is a runaway guard, not a semantic limit: a DOM nested
  // deeper than any real one still yields an anchor rather than looping.
  it('terminates on a pathologically deep DOM', () => {
    const scroller = makeScroller();
    const row = box(SCROLLER_TOP - 10, 60);
    scroller.appendChild(span(row, 200));

    const anchor = pickAnchor(scroller);
    expect(anchor).not.toBeNull();
    expect(scroller.contains(anchor as Node)).toBe(true);
  });
});
