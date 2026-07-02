import { describe, it, expect } from 'vitest';
import { latchedSpringMode, SPRING_MODE_HOLD_MS } from './springAnimationLatch';

describe('latchedSpringMode', () => {
  it("returns 'spring' while within the hold window", () => {
    expect(latchedSpringMode(1000, 700, 500)).toBe('spring'); // 300 < 500
    expect(latchedSpringMode(1000, 999, 500)).toBe('spring'); // 1 < 500
    expect(latchedSpringMode(1000, 1000, 500)).toBe('spring'); // 0 < 500 (same frame)
  });

  it("returns 'instant' at and beyond the hold boundary", () => {
    expect(latchedSpringMode(1000, 500, 500)).toBe('instant'); // exactly 500, not < 500
    expect(latchedSpringMode(1000, 499, 500)).toBe('instant'); // 501 > 500
    expect(latchedSpringMode(5000, 700, 500)).toBe('instant'); // long gap
  });

  it("returns 'instant' for a never-stamped pane once time has advanced", () => {
    // lastLiveContentAt defaults to 0; any real performance.now() reading
    // is far beyond the hold window, so an idle pane never springs.
    expect(latchedSpringMode(SPRING_MODE_HOLD_MS, 0, SPRING_MODE_HOLD_MS)).toBe('instant');
    expect(latchedSpringMode(10_000, 0, SPRING_MODE_HOLD_MS)).toBe('instant');
  });

  it('treats a future stamp (clock skew) as within window', () => {
    // Defensive: a stamp slightly ahead of `now` still reads as spring
    // rather than flipping to instant on a transient negative delta.
    expect(latchedSpringMode(1000, 1001, 500)).toBe('spring');
  });

  it('pins SPRING_MODE_HOLD_MS to 500ms', () => {
    // Pure tuning value (see the constant's comment for the retired
    // HOLD > RETAIN history); pinned so a drive-by change is deliberate.
    expect(SPRING_MODE_HOLD_MS).toBe(500);
  });
});
