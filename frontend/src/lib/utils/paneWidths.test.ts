import { describe, expect, it } from 'vitest';
import {
  FALLBACK_PANE_WIDTH_PX,
  MAX_PANE_WIDTH_PX,
  minAnchorPaneWidths,
  normalizePaneWidthPx,
  resolvePaneBoundaryDrag,
  type PaneBoundaryDrag,
} from './paneWidths';

const MIN = 560;

function drag(overrides: Partial<PaneBoundaryDrag>): PaneBoundaryDrag {
  return {
    widths: [MIN, MIN, MIN],
    leftIndex: 0,
    hasRightPane: true,
    deltaPx: 0,
    minPaneWidth: MIN,
    overflowPx: 0,
    zeroSum: false,
    ...overrides,
  };
}

describe('normalizePaneWidthPx', () => {
  it('passes finite positive widths through and caps at the maximum', () => {
    expect(normalizePaneWidthPx(880)).toBe(880);
    expect(normalizePaneWidthPx(MAX_PANE_WIDTH_PX + 1)).toBe(MAX_PANE_WIDTH_PX);
  });

  it('falls back for garbage, including sub-1px widths', () => {
    expect(normalizePaneWidthPx(0)).toBe(FALLBACK_PANE_WIDTH_PX);
    expect(normalizePaneWidthPx(-5)).toBe(FALLBACK_PANE_WIDTH_PX);
    expect(normalizePaneWidthPx(0.5)).toBe(FALLBACK_PANE_WIDTH_PX);
    expect(normalizePaneWidthPx(Number.NaN)).toBe(FALLBACK_PANE_WIDTH_PX);
    expect(normalizePaneWidthPx(Number.POSITIVE_INFINITY)).toBe(FALLBACK_PANE_WIDTH_PX);
  });
});

describe('resolvePaneBoundaryDrag - overflow (strip scrolls)', () => {
  it('growing the left pane shifts the rest instead of shrinking the neighbor', () => {
    const result = resolvePaneBoundaryDrag(drag({
      widths: [MIN, 800, MIN],
      deltaPx: 300,
      overflowPx: 400,
    }));

    expect(result).toEqual([MIN + 300, 800, MIN]);
  });

  it('a grow-then-shrink round trip across two gestures restores the layout', () => {
    // Gesture 1: all panes at min, overflowed; grow the first pane.
    const grown = resolvePaneBoundaryDrag(drag({
      widths: [MIN, MIN, MIN],
      deltaPx: 300,
      overflowPx: 200,
    }));
    expect(grown).toEqual([MIN + 300, MIN, MIN]);

    // Gesture 2: drag it back. The overflow grew by the same 300, so
    // the shrink is fully absorbed by scroll — the neighbor must NOT
    // have grown (that would require resizing every pane to undo).
    const restored = resolvePaneBoundaryDrag(drag({
      widths: grown!,
      deltaPx: -300,
      overflowPx: 500,
    }));
    expect(restored).toEqual([MIN, MIN, MIN]);
  });

  it('shrinking beyond the available overflow hands the remainder to the neighbor', () => {
    const result = resolvePaneBoundaryDrag(drag({
      widths: [1000, MIN, MIN],
      deltaPx: -400,
      overflowPx: 150,
    }));

    // 150 of the shrink is absorbed by scroll; the remaining 250 keeps
    // the strip filling the window by growing the neighbor.
    expect(result).toEqual([600, MIN + 250, MIN]);
  });

  it('never shrinks the left pane below the minimum', () => {
    const result = resolvePaneBoundaryDrag(drag({
      widths: [700, MIN],
      deltaPx: -500,
      overflowPx: 1000,
    }));

    expect(result).toEqual([MIN, MIN]);
  });
});

describe('resolvePaneBoundaryDrag - fit (strip fills the window)', () => {
  it('growing trades with the neighbor first, then pushes into overflow', () => {
    // Neighbor has 240 above min; a 400 drag takes those 240 and the
    // remaining 160 becomes new overflow.
    const result = resolvePaneBoundaryDrag(drag({
      widths: [800, 800],
      deltaPx: 400,
      overflowPx: 0,
    }));

    expect(result).toEqual([1200, MIN]);
  });

  it('shrinking hands all freed space to the neighbor', () => {
    const result = resolvePaneBoundaryDrag(drag({
      widths: [800, 800],
      deltaPx: -200,
      overflowPx: 0,
    }));

    expect(result).toEqual([600, 1000]);
  });

  it('is continuous when a gesture crosses back over zero delta', () => {
    const start = [800, 800];
    // Same snapshot, delta swings positive then negative: both resolve
    // from the start widths, meeting exactly at the start on zero.
    expect(resolvePaneBoundaryDrag(drag({ widths: start, deltaPx: 300 }))).toEqual([1100, MIN]);
    expect(resolvePaneBoundaryDrag(drag({ widths: start, deltaPx: 0 }))).toEqual(start);
    expect(resolvePaneBoundaryDrag(drag({ widths: start, deltaPx: -100 }))).toEqual([700, 900]);
  });

  it('growing against a neighbor already at min pushes straight into overflow', () => {
    const result = resolvePaneBoundaryDrag(drag({
      widths: [800, MIN],
      deltaPx: 150,
      overflowPx: 0,
    }));

    expect(result).toEqual([950, MIN]);
  });

  it('treats fractional overflow within the epsilon as fit for growth and absorbs it on shrink', () => {
    expect(resolvePaneBoundaryDrag(drag({
      widths: [800, 800],
      deltaPx: 100,
      overflowPx: 0.5,
    }))).toEqual([900, 700]);

    expect(resolvePaneBoundaryDrag(drag({
      widths: [800, 800],
      deltaPx: -100,
      overflowPx: 0.5,
    }))).toEqual([700, 899.5]);
  });
});

describe('resolvePaneBoundaryDrag - zero-sum (Alt)', () => {
  it('trades space with the right neighbor and keeps the total, even in overflow', () => {
    const result = resolvePaneBoundaryDrag(drag({
      widths: [900, 900, MIN],
      deltaPx: 200,
      overflowPx: 500,
      zeroSum: true,
    }));

    expect(result).toEqual([1100, 700, MIN]);
  });

  it('clamps at both minimums', () => {
    const result = resolvePaneBoundaryDrag(drag({
      widths: [700, 700],
      deltaPx: 1000,
      zeroSum: true,
    }));

    expect(result).toEqual([840, MIN]);
  });

  it('is a no-op when the pair cannot fit two minimums', () => {
    expect(resolvePaneBoundaryDrag(drag({
      widths: [500, 500],
      deltaPx: 100,
      zeroSum: true,
    }))).toBeNull();
  });

  it('keeps the trade total-preserving when both panes sit near the width cap', () => {
    const result = resolvePaneBoundaryDrag(drag({
      widths: [MAX_PANE_WIDTH_PX, MAX_PANE_WIDTH_PX],
      deltaPx: -500,
      overflowPx: 5000,
      zeroSum: true,
    }));

    // The neighbor cannot exceed MAX, so the left pane cannot give up
    // any width: the trade clamps to a no-op rather than losing total.
    expect(result).toEqual([MAX_PANE_WIDTH_PX, MAX_PANE_WIDTH_PX]);
  });
});

describe('resolvePaneBoundaryDrag - end handle', () => {
  it('growing the last pane extends the strip without touching anyone else', () => {
    const result = resolvePaneBoundaryDrag(drag({
      widths: [MIN, MIN],
      leftIndex: 1,
      hasRightPane: false,
      deltaPx: 250,
      overflowPx: 100,
    }));

    expect(result).toEqual([MIN, MIN + 250]);
  });

  it('shrinking beyond overflow hands the remainder to the left neighbor', () => {
    const result = resolvePaneBoundaryDrag(drag({
      widths: [MIN, 900],
      leftIndex: 1,
      hasRightPane: false,
      deltaPx: -300,
      overflowPx: 100,
    }));

    expect(result).toEqual([MIN + 200, 600]);
  });

  it('zero-sum trades with the left neighbor', () => {
    const result = resolvePaneBoundaryDrag(drag({
      widths: [900, 700],
      leftIndex: 1,
      hasRightPane: false,
      deltaPx: 150,
      zeroSum: true,
    }));

    expect(result).toEqual([750, 850]);
  });

  it('shrinking a single pane has no recipient and just clamps at the minimum', () => {
    const result = resolvePaneBoundaryDrag(drag({
      widths: [900],
      leftIndex: 0,
      hasRightPane: false,
      deltaPx: -500,
      overflowPx: 0,
    }));

    expect(result).toEqual([MIN]);
  });

  it('zero-sum on a single-pane end handle has no neighbor and is a no-op', () => {
    // leftIndex 0, no right pane → neighbor index -1 → nothing to trade with.
    expect(resolvePaneBoundaryDrag(drag({
      widths: [900],
      leftIndex: 0,
      hasRightPane: false,
      deltaPx: 200,
      zeroSum: true,
    }))).toBeNull();
  });
});

describe('resolvePaneBoundaryDrag - guards', () => {
  it('resolves a zero delta back to the start snapshot (the caller diffs current state)', () => {
    expect(resolvePaneBoundaryDrag(drag({ deltaPx: 0 }))).toEqual([MIN, MIN, MIN]);
  });

  it('returns null for bad indexes and bad widths', () => {
    expect(resolvePaneBoundaryDrag(drag({ leftIndex: -1 }))).toBeNull();
    expect(resolvePaneBoundaryDrag(drag({ leftIndex: 3 }))).toBeNull();
    expect(resolvePaneBoundaryDrag(drag({ leftIndex: 2, hasRightPane: true }))).toBeNull();
    expect(resolvePaneBoundaryDrag(drag({ widths: [MIN, Number.NaN], leftIndex: 0, deltaPx: 50 }))).toBeNull();
    expect(resolvePaneBoundaryDrag(drag({ deltaPx: Number.NaN }))).toBeNull();
  });

  it('caps growth at the maximum pane width', () => {
    const result = resolvePaneBoundaryDrag(drag({
      widths: [MAX_PANE_WIDTH_PX - 50, MIN],
      deltaPx: 500,
      overflowPx: 300,
    }));

    expect(result).toEqual([MAX_PANE_WIDTH_PX, MIN]);
  });
});

describe('minAnchorPaneWidths', () => {
  it('scales widths down so the smallest sits at the minimum', () => {
    expect(minAnchorPaneWidths([1120, 2240], MIN)).toEqual([MIN, 1120]);
  });

  it('is a no-op when already anchored or empty', () => {
    expect(minAnchorPaneWidths([MIN, 900], MIN)).toBeNull();
    expect(minAnchorPaneWidths([], MIN)).toBeNull();
  });

  it('never produces a width below the minimum', () => {
    const anchored = minAnchorPaneWidths([561, 5000], MIN);
    expect(anchored![0]).toBeGreaterThanOrEqual(MIN);
  });
});
