import { describe, expect, it, vi } from 'vitest';
import {
  ACTIVITY_RUN_FOLD_EASING,
  activityRunFoldDeadlineMs,
  activityRunFoldDurationMs,
  prefersReducedMotion,
} from './activityRunFold';

describe('activityRunFoldDurationMs', () => {
  it('scales with the height it has to close', () => {
    expect(activityRunFoldDurationMs(400)).toBeGreaterThan(activityRunFoldDurationMs(80));
  });

  it('stays inside a range a reader will sit through', () => {
    // The bounds matter more than the curve: below the floor the fold reads as
    // a flicker rather than a close, and above the ceiling a finished run is
    // still in the way.
    for (const px of [0, 1, 40, 200, 480, 4000]) {
      const ms = activityRunFoldDurationMs(px);
      expect(ms).toBeGreaterThanOrEqual(320);
      expect(ms).toBeLessThanOrEqual(600);
    }
  });

  it('gives a degenerate height the floor rather than a zero-length fold', () => {
    // Callers measure before they animate, and a zero here would produce an
    // animation that finishes in the frame it starts — the instant jump the
    // whole mechanism exists to remove.
    for (const px of [0, -1, Number.NaN]) {
      expect(activityRunFoldDurationMs(px)).toBe(320);
    }
  });

  it('grows sub-linearly, so a long run does not feel like a different gesture', () => {
    const short = activityRunFoldDurationMs(100);
    const long = activityRunFoldDurationMs(400);
    expect(long / short).toBeLessThan(4);
  });
});

describe('activityRunFoldDeadlineMs', () => {
  it('leaves the animation room to finish on its own first', () => {
    // The deadline is the backstop for a tab whose animations stopped
    // advancing, not a second timing source competing with the first.
    expect(activityRunFoldDeadlineMs(200)).toBeGreaterThan(200);
  });
});

describe('prefersReducedMotion', () => {
  it('reads the media query when one is available', () => {
    const matchMedia = vi.fn(() => ({ matches: true }) as MediaQueryList);
    vi.stubGlobal('matchMedia', matchMedia);
    try {
      expect(prefersReducedMotion()).toBe(true);
      expect(matchMedia).toHaveBeenCalledWith('(prefers-reduced-motion: reduce)');
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('reports no stated preference when the environment has no matchMedia', () => {
    // Absence is not a failure to report — it means nobody asked for reduced
    // motion, which is the same answer as a query that does not match.
    vi.stubGlobal('matchMedia', undefined);
    try {
      expect(prefersReducedMotion()).toBe(false);
    } finally {
      vi.unstubAllGlobals();
    }
  });
});

describe('ACTIVITY_RUN_FOLD_EASING', () => {
  /** `[x1, y1, x2, y2]` from the declared curve. */
  function controlPoints(): number[] {
    const match = /^cubic-bezier\(([^)]+)\)$/.exec(ACTIVITY_RUN_FOLD_EASING);
    expect(match).not.toBeNull();
    const points = (match?.[1] ?? '').split(',').map((n) => Number(n.trim()));
    expect(points).toHaveLength(4);
    return points;
  }

  it('settles into its landing instead of stopping dead', () => {
    // With the second point at (1, 1) the end tangent is degenerate and the box
    // is still at full speed when it reaches zero height, which reads as violent
    // however long the fold took. Pulling x2 in gives the tangent a direction,
    // so the terminal velocity is zero and the height eases in.
    const [, , x2, y2] = controlPoints();
    expect(y2).toBe(1);
    expect(x2).toBeLessThan(1);
  });

  it('does not crawl the last pixels either', () => {
    // The opposite failure, and the reason x2 is not free: the lower it goes the
    // more of the duration covers the final handful of pixels, and the run reads
    // as hesitating — visibly done well before it is finished. Late x2 keeps the
    // deceleration in the tail.
    const [, , x2] = controlPoints();
    expect(x2).toBeGreaterThanOrEqual(0.7);
  });

  it('does not hold still before it moves', () => {
    // The other half, and the one that decides how the fold READS. With y1 at
    // zero — a textbook ease-in — the box is motionless for roughly the opening
    // third and the whole close is crammed into what is left, so the fold feels
    // far sharper than its duration says. Off zero it starts on the first frame.
    const [x1, y1] = controlPoints();
    expect(y1).toBeGreaterThan(0);
    // And still an ease-in, not a linear ramp: the opening speed stays below
    // the average, or there is no acceleration left to soften.
    expect(y1 / x1).toBeLessThan(1);
  });
});
