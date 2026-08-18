import { describe, expect, it } from 'vitest';
import {
  clampPopoverPosition,
  clipPathRule,
  intersectClipBoundary,
  oppositeOf,
  overflowsPrimaryAxis,
  placePopover,
  type PopoverPlacement,
} from './popoverGeometry';

const ANCHOR = { top: 100, left: 200, right: 260, bottom: 120 };
const FLOAT = { width: 100, height: 50 };

describe('placePopover', () => {
  it.each<[PopoverPlacement, { top: number; left: number }]>([
    ['bottom-start', { top: 124, left: 200 }],
    ['bottom-end', { top: 124, left: 160 }],
    ['top-start', { top: 46, left: 200 }],
    ['top-end', { top: 46, left: 160 }],
    ['right-start', { top: 100, left: 264 }],
    ['left-start', { top: 100, left: 96 }],
  ])('%s', (placement, expected) => {
    expect(placePopover(ANCHOR, FLOAT, placement, 4)).toEqual(expected);
  });
});

describe('oppositeOf', () => {
  it('pairs each placement with its natural flip on the same axis', () => {
    expect(oppositeOf('bottom-start')).toBe('top-start');
    expect(oppositeOf('bottom-end')).toBe('top-end');
    expect(oppositeOf('top-start')).toBe('bottom-start');
    expect(oppositeOf('top-end')).toBe('bottom-end');
    expect(oppositeOf('right-start')).toBe('left-start');
    expect(oppositeOf('left-start')).toBe('right-start');
  });
});

describe('overflowsPrimaryAxis', () => {
  it('bottom placements overflow past the viewport bottom', () => {
    expect(overflowsPrimaryAxis({ top: 760, left: 0 }, FLOAT, 'bottom-start', 1000, 800)).toBe(true);
    expect(overflowsPrimaryAxis({ top: 700, left: 0 }, FLOAT, 'bottom-end', 1000, 800)).toBe(false);
  });

  it('top placements overflow past the viewport top', () => {
    expect(overflowsPrimaryAxis({ top: -1, left: 0 }, FLOAT, 'top-start', 1000, 800)).toBe(true);
    expect(overflowsPrimaryAxis({ top: 0, left: 0 }, FLOAT, 'top-end', 1000, 800)).toBe(false);
  });

  it('side placements overflow past their viewport edge', () => {
    expect(overflowsPrimaryAxis({ top: 0, left: 950 }, FLOAT, 'right-start', 1000, 800)).toBe(true);
    expect(overflowsPrimaryAxis({ top: 0, left: -1 }, FLOAT, 'left-start', 1000, 800)).toBe(true);
    expect(overflowsPrimaryAxis({ top: 0, left: 500 }, FLOAT, 'right-start', 1000, 800)).toBe(false);
  });
});

describe('clampPopoverPosition', () => {
  it('leaves an in-bounds position untouched with no height limit', () => {
    expect(clampPopoverPosition({ top: 100, left: 200 }, FLOAT, 1000, 800, null)).toEqual({
      top: 100,
      left: 200,
      maxHeight: undefined,
    });
  });

  it('clamps to the viewport margins when no clip applies', () => {
    const clamped = clampPopoverPosition({ top: 790, left: 995 }, FLOAT, 1000, 800, null);
    expect(clamped.top).toBe(800 - 8 - FLOAT.height);
    expect(clamped.left).toBe(1000 - 8 - FLOAT.width);
    // The vertical clamp moved the popover, so it gets the scroll cap.
    expect(clamped.maxHeight).toBe(800 - 16);
  });

  it('caps a float taller than the viewport to the margin-inset height', () => {
    const clamped = clampPopoverPosition({ top: 8, left: 8 }, { width: 100, height: 900 }, 1000, 800, null);
    expect(clamped.maxHeight).toBe(784);
  });

  // The blocking review finding: fit math and clip math must use the SAME
  // bounds. An end-aligned popover whose natural position starts left of
  // the clip boundary would otherwise OPEN with its leading columns already
  // cut off behind the sidebar, permanently invisible.
  it('clamps the open position into the clip boundary, not just the viewport', () => {
    const clip = { top: 0, left: 300, right: 1000, bottom: 800 };
    const clamped = clampPopoverPosition({ top: 100, left: 250 }, FLOAT, 1000, 800, clip);
    expect(clamped.left).toBe(300);
    expect(clamped.top).toBe(100);
  });

  it('derives the height cap from the clip boundary when it is shorter than the viewport', () => {
    const clip = { top: 100, left: 300, right: 1000, bottom: 400 };
    const clamped = clampPopoverPosition({ top: 90, left: 400 }, { width: 100, height: 500 }, 1000, 800, clip);
    expect(clamped.top).toBe(100);
    expect(clamped.maxHeight).toBe(300);
  });

  it('pins a float wider than the clip to the boundary near edge', () => {
    const clip = { top: 0, left: 300, right: 350, bottom: 800 };
    const clamped = clampPopoverPosition({ top: 100, left: 200 }, FLOAT, 1000, 800, clip);
    // max < min in the horizontal clamp: pinned at the boundary's left;
    // the overhang past the right edge is the clip-path's problem.
    expect(clamped.left).toBe(300);
  });

  it('keeps the viewport margin when the clip reaches the viewport edge', () => {
    const clip = { top: 0, left: 0, right: 1000, bottom: 800 };
    const clamped = clampPopoverPosition({ top: 0, left: 0 }, FLOAT, 1000, 800, clip);
    expect(clamped.top).toBe(8);
    expect(clamped.left).toBe(8);
  });
});

describe('intersectClipBoundary', () => {
  it('returns the boundary unchanged when fully inside the viewport', () => {
    const b = { top: 0, left: 300, right: 900, bottom: 700 };
    expect(intersectClipBoundary(b, 1000, 800)).toEqual(b);
  });

  it('intersects a boundary that overhangs the viewport', () => {
    expect(intersectClipBoundary({ top: -10, left: -20, right: 1200, bottom: 900 }, 1000, 800)).toEqual({
      top: 0,
      left: 0,
      right: 1000,
      bottom: 800,
    });
  });

  it('answers null for a zero-size boundary (no clip, never clip-everything)', () => {
    expect(intersectClipBoundary({ top: 0, left: 0, right: 0, bottom: 0 }, 1000, 800)).toBeNull();
    expect(intersectClipBoundary({ top: 5, left: 5, right: 5, bottom: 500 }, 1000, 800)).toBeNull();
  });

  it('answers null when the boundary lies fully outside the viewport', () => {
    expect(intersectClipBoundary({ top: 0, left: 1200, right: 1400, bottom: 800 }, 1000, 800)).toBeNull();
  });
});

describe('clipPathRule', () => {
  const CLIP = { top: 50, left: 300, right: 700, bottom: 600 };

  it('emits nothing while the element sits fully inside the boundary', () => {
    expect(clipPathRule(CLIP, 100, 400, 100, 50)).toBe('');
  });

  // inset() argument order is top/right/bottom/left — a swap renders as a
  // cut on the wrong edge, so each arm is pinned individually.
  it('cuts the left edge', () => {
    expect(clipPathRule(CLIP, 100, 280, 100, 50)).toBe('clip-path: inset(0px 0px 0px 20px);');
  });

  it('cuts the right edge', () => {
    expect(clipPathRule(CLIP, 100, 650, 100, 50)).toBe('clip-path: inset(0px 50px 0px 0px);');
  });

  it('cuts the top edge', () => {
    expect(clipPathRule(CLIP, 40, 400, 100, 50)).toBe('clip-path: inset(10px 0px 0px 0px);');
  });

  it('cuts the bottom edge', () => {
    expect(clipPathRule(CLIP, 580, 400, 100, 50)).toBe('clip-path: inset(0px 0px 30px 0px);');
  });

  it('cuts multiple edges at once', () => {
    expect(clipPathRule(CLIP, 40, 280, 500, 600)).toBe(
      'clip-path: inset(10px 80px 40px 20px);',
    );
  });
});
