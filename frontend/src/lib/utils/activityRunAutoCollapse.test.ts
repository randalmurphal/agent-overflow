import { describe, expect, it } from 'vitest';
import {
  AUTO_COLLAPSE_TAIL_DISTANCE_VIEWPORTS,
  AUTO_COLLAPSE_VIEWPORT_MARGIN_PX,
  activityRunOutOfSight,
  type ActivityRunGateGeometry,
} from './activityRunAutoCollapse';

// A 600px viewport over 10000px of content, reader position stated per case.
function geometry(overrides: Partial<ActivityRunGateGeometry>): ActivityRunGateGeometry {
  return {
    top: 0,
    bottom: 400,
    scrollTop: 5000,
    viewport: 600,
    totalSize: 10000,
    ...overrides,
  };
}

describe('activityRunOutOfSight', () => {
  it('accepts a run far above a reader far from its tail', () => {
    expect(activityRunOutOfSight(geometry({}))).toBe(true);
  });

  it('accepts a run far below a reader scrolled up past it', () => {
    // Reader near the top, run between them and the tail: the shrink happens
    // entirely below the viewport, which moves nothing the reader sees.
    expect(
      activityRunOutOfSight(geometry({ top: 7000, bottom: 7400, scrollTop: 0 })),
    ).toBe(true);
  });

  it('refuses any partially visible run, whatever the tail distance', () => {
    // Straddling the top edge, the bottom edge, and fully inside.
    expect(
      activityRunOutOfSight(geometry({ top: 4900, bottom: 5100 })),
    ).toBe(false);
    expect(
      activityRunOutOfSight(geometry({ top: 5500, bottom: 5700 })),
    ).toBe(false);
    expect(
      activityRunOutOfSight(geometry({ top: 5200, bottom: 5400 })),
    ).toBe(false);
  });

  it('refuses a run inside the margin past either viewport edge', () => {
    // A hair off-screen is one scrollbar nudge from being back on it — the
    // reader must not find it collapsed with no idea when that happened.
    const nearAbove = 5000 - AUTO_COLLAPSE_VIEWPORT_MARGIN_PX / 2;
    expect(
      activityRunOutOfSight(geometry({ top: nearAbove - 200, bottom: nearAbove })),
    ).toBe(false);
    const nearBelow = 5600 + AUTO_COLLAPSE_VIEWPORT_MARGIN_PX / 2;
    expect(
      activityRunOutOfSight(geometry({ top: nearBelow, bottom: nearBelow + 200 })),
    ).toBe(false);
  });

  it('refuses a run near the tail even when it is far off-screen', () => {
    // Distance from the TAIL, not from the viewport: a reader who scrolled up
    // to check something and comes back must find the latest runs exactly as
    // they left them.
    expect(
      activityRunOutOfSight(
        geometry({ top: 9000, bottom: 9600, scrollTop: 0 }),
      ),
    ).toBe(false);
  });

  it('flips exactly at the tail-distance threshold', () => {
    const threshold = 600 * AUTO_COLLAPSE_TAIL_DISTANCE_VIEWPORTS;
    // Bottom exactly one threshold from the end: not yet past it.
    expect(
      activityRunOutOfSight(geometry({ top: 9000, bottom: 10000 - threshold, scrollTop: 0 })),
    ).toBe(false);
    expect(
      activityRunOutOfSight(
        geometry({ top: 9000 - 1, bottom: 10000 - threshold - 1, scrollTop: 0 }),
      ),
    ).toBe(true);
  });
});
