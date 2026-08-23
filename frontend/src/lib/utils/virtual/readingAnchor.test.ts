import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  measureReadingAnchorShift,
  sampleReadingAnchor,
  type AnchorRow,
} from './readingAnchor';

// happy-dom reports zero geometry, so every rect in this suite is stubbed.
// Rects are the ONLY input the module reads besides the hit test, which
// makes stubbing them a faithful model rather than a shortcut.
function stubRect(el: Element, top: number): void {
  vi.spyOn(el, 'getBoundingClientRect').mockImplementation(
    () => ({ top, left: 0, width: 800, height: 100, bottom: top + 100, right: 800, x: 0, y: top, toJSON: () => ({}) }) as DOMRect,
  );
}

function makeScroller(): { scroller: HTMLElement; row: HTMLElement; inner: HTMLElement } {
  const scroller = document.createElement('div');
  const row = document.createElement('div');
  const inner = document.createElement('p');
  row.appendChild(inner);
  scroller.appendChild(row);
  document.body.appendChild(scroller);
  stubRect(scroller, 0);
  return { scroller, row, inner };
}

let hit: Element | null = null;

beforeEach(() => {
  hit = null;
  vi.spyOn(document, 'elementFromPoint').mockImplementation(() => hit);
});

afterEach(() => {
  vi.restoreAllMocks();
  document.body.innerHTML = '';
});

describe('sampleReadingAnchor', () => {
  it('records the hit element offset from its own row top', () => {
    const { scroller, row, inner } = makeScroller();
    stubRect(row, -300); // row top 300px above the viewport top
    stubRect(inner, -40);
    hit = inner;

    const anchor = sampleReadingAnchor({
      scroller,
      rowFor: (el): AnchorRow | undefined => (el === inner || el === row ? { el: row, index: 7 } : undefined),
    });

    expect(anchor).not.toBeNull();
    expect(anchor?.rowEl).toBe(row);
    expect(anchor?.anchorEl).toBe(inner);
    expect(anchor?.intraRowOffset).toBe(260); // -40 - (-300)
  });

  it('returns null when the hit resolves to no row', () => {
    const { scroller, inner } = makeScroller();
    hit = inner;
    expect(sampleReadingAnchor({ scroller, rowFor: () => undefined })).toBeNull();
  });

  it('returns null when the hit IS the row (no sub-row information)', () => {
    // The row wrapper's offset from its own top is identically zero and can
    // never change, so such an anchor could only ever report a zero shift.
    // Rejecting it at sample time keeps "we have an anchor" meaningful.
    const { scroller, row } = makeScroller();
    stubRect(row, -300);
    hit = row;
    expect(sampleReadingAnchor({ scroller, rowFor: () => ({ el: row, index: 7 }) })).toBeNull();
  });

  it('returns null when nothing is painted at the point', () => {
    const { scroller, row } = makeScroller();
    hit = null;
    expect(sampleReadingAnchor({ scroller, rowFor: () => ({ el: row, index: 7 }) })).toBeNull();
  });

  it('returns null for a zero-sized scroller (hidden pane, unmeasured mount)', () => {
    const { scroller, row, inner } = makeScroller();
    vi.spyOn(scroller, 'getBoundingClientRect').mockReturnValue(
      { top: 0, left: 0, width: 0, height: 0, bottom: 0, right: 0, x: 0, y: 0, toJSON: () => ({}) } as DOMRect,
    );
    hit = inner;
    expect(sampleReadingAnchor({ scroller, rowFor: () => ({ el: row, index: 7 }) })).toBeNull();
  });

  it('hit-tests one px inside the content box, at the horizontal center', () => {
    const { scroller, row, inner } = makeScroller();
    vi.spyOn(scroller, 'getBoundingClientRect').mockReturnValue(
      { top: 120, left: 40, width: 800, height: 600, bottom: 720, right: 840, x: 40, y: 120, toJSON: () => ({}) } as DOMRect,
    );
    Object.defineProperty(scroller, 'clientTop', { value: 3, configurable: true });
    stubRect(row, 0);
    stubRect(inner, 0);
    hit = inner;

    sampleReadingAnchor({ scroller, rowFor: () => ({ el: row, index: 7 }) });

    expect(document.elementFromPoint).toHaveBeenCalledWith(440, 124); // left + width/2, top + clientTop + 1
  });
});

describe('measureReadingAnchorShift', () => {
  it('reports intra-row movement, ignoring movement of the row itself', () => {
    const { row, inner } = makeScroller();
    // Sampled: row at -300, anchor at -40 → intraRowOffset 260.
    // Now the whole row moved down 500px (rows above it grew, already
    // compensated exactly by the engine) AND 30px appeared inside the row
    // above the anchor. Only the 30 is ours to report.
    stubRect(row, 200);
    stubRect(inner, 490);

    expect(measureReadingAnchorShift({ rowEl: row, anchorEl: inner, intraRowOffset: 260 })).toBe(30);
  });

  it('reports zero when nothing moved inside the row', () => {
    const { row, inner } = makeScroller();
    stubRect(row, -300);
    stubRect(inner, -40);
    expect(measureReadingAnchorShift({ rowEl: row, anchorEl: inner, intraRowOffset: 260 })).toBe(0);
  });

  it('reports negative for a shrink above the reading position', () => {
    const { row, inner } = makeScroller();
    stubRect(row, -300);
    stubRect(inner, -90);
    expect(measureReadingAnchorShift({ rowEl: row, anchorEl: inner, intraRowOffset: 260 })).toBe(-50);
  });

  it('returns null once the anchor element leaves the DOM', () => {
    const { row, inner } = makeScroller();
    inner.remove();
    expect(measureReadingAnchorShift({ rowEl: row, anchorEl: inner, intraRowOffset: 260 })).toBeNull();
  });

  it('returns null once the row element leaves the DOM', () => {
    const { row, inner } = makeScroller();
    row.remove();
    expect(measureReadingAnchorShift({ rowEl: row, anchorEl: inner, intraRowOffset: 260 })).toBeNull();
  });
});
